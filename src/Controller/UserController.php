<?php

namespace App\Controller;

use App\Form\{CreateUserRequest, CreateUserRequestType};
use App\Repository\{UserRepository};
use App\User\Exception\InvalidInviteException;
use App\User\UserManager;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Bundle\FrameworkBundle\Controller\Controller;
use Symfony\Component\Form\Extension\Core\Type\SubmitType;
use Symfony\Component\Form\{FormError, FormInterface};
use Symfony\Component\HttpFoundation\{Request, Response};

class UserController extends Controller
{
    public function register(
        string $inviteCode,
        Request $request,
        EntityManagerInterface $em,
        UserManager $userManager,
        UserRepository $userRepository
    ): Response {
        $createUserRequest = new CreateUserRequest();
        $createUserRequest->inviteCode = $inviteCode;
        $form = $this->createRegisterForm($createUserRequest, $inviteCode);

        $form->handleRequest($request);

        if ($form->isSubmitted() && $form->isValid()) {
            try {
                $user = $userManager->createUserByInviteCode(
                    $createUserRequest->username,
                    $createUserRequest->password,
                    $createUserRequest->email,
                    $createUserRequest->inviteCode
                );
            } catch (InvalidInviteException $ex) {
                // @FIXME refactor InvalidInviteException to proper validator
                $form->get('inviteCode')->addError(new FormError('Invalid invite code'));

                return $this->render('User/register.html.twig', ['form' => $form->createView()]);
            }

            $userRepository->add($user);
            $em->flush();

            return $this->redirectToRoute('index');
        }

        return $this->render('User/register.html.twig', ['form' => $form->createView()]);
    }

    private function createRegisterForm(CreateUserRequest $createUserRequest, string $inviteCode): FormInterface
    {
        $form = $this->createForm(CreateUserRequestType::class, $createUserRequest, [
            'action' => $this->generateUrl('user_register', ['inviteCode' => $inviteCode]),
        ]);

        $form->add('submit', SubmitType::class);

        return $form;
    }
}