<?php
declare(strict_types=1);

namespace App\Api\V1\Controller;

use App\Entity\{ApiToken, User};
use App\Repository\{ApiTokenRepository, UserRepository};
use App\Security\Token\AuthenticatedApiToken;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\HttpFoundation\{JsonResponse, Request};
use Symfony\Component\Security\Core\Authentication\Token\Storage\TokenStorageInterface;
use Symfony\Component\Security\Core\Encoder\UserPasswordEncoderInterface;

class SecurityController extends AbstractApiController
{
    public function login(
        Request $request,
        EntityManagerInterface $em,
        UserRepository $userRepo,
        ApiTokenRepository $tokenRepo,
        UserPasswordEncoderInterface $passwordEncoder
    ): JsonResponse {
        $username = $request->request->get('username');
        $password = $request->request->get('password');

        /** @var User $user */
        if (null === $user = $userRepo->findOneBy(['username' => $username])) {
            return $this->createJsonResponse(null, [], JsonResponse::HTTP_UNAUTHORIZED, 'User not found');
        }

        if (!$passwordEncoder->isPasswordValid($user, $password)) {
            return $this->createJsonResponse(null, [], JsonResponse::HTTP_UNAUTHORIZED, 'Invalid password');
        }

        $apiToken = new ApiToken($user);
        $tokenRepo->add($apiToken);

        try {
            $em->flush();
        } catch (\Exception $ex) {
            return $this->createJsonResponse(null, [], JsonResponse::HTTP_INTERNAL_SERVER_ERROR, 'Token persisting error');
        }

        return $this->createJsonResponse($apiToken->getKey());
    }

    public function logout(TokenStorageInterface $tokenStorage, ApiTokenRepository $apiTokenRepo, EntityManagerInterface $em): JsonResponse
    {
        if (null === $token = $tokenStorage->getToken()) {
            return $this->createJsonResponse(null,[],JsonResponse::HTTP_INTERNAL_SERVER_ERROR, 'Can\'t retrieve user token.');
        }

        if (!$token instanceof AuthenticatedApiToken) {
            return $this->createJsonResponse(null, [], JsonResponse::HTTP_INTERNAL_SERVER_ERROR, 'Invalid session token type retrieved.');
        }

        if (null === $tokenKey = $token->getTokenKey()) {
            return $this->createJsonResponse(null,[],JsonResponse::HTTP_INTERNAL_SERVER_ERROR, 'Can\'t retrieve token key from the session.');
        }

        if (null === $apiToken = $apiTokenRepo->findOneBy(['key' => $tokenKey])) {
            return $this->createJsonResponse(null,[],JsonResponse::HTTP_INTERNAL_SERVER_ERROR, 'API token with such key not found in the database.');
        }

        $em->remove($apiToken);

        try {
            $em->flush();
        } catch (\Exception $ex) {
            return $this->createJsonResponse(null,[],JsonResponse::HTTP_INTERNAL_SERVER_ERROR, 'API token deauthentication failure.');
        }

        return $this->createJsonResponse(null,[],JsonResponse::HTTP_OK, 'Successfully logged out.');
    }
}