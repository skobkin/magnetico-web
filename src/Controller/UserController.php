<?php

namespace App\Controller;

use App\Form\{CreateUserRequestType};
use App\FormRequest\CreateUserRequest;
use App\User\Exception\InvalidInviteException;
use App\User\{InviteManager, UserManager};
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
        InviteManager $inviteManager
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

                $inviteManager->createInvitesForUser($user);
            } catch (InvalidInviteException $ex) {
                // @FIXME refactor InvalidInviteException to proper validator
                $form->get('inviteCode')->addError(new FormError('Invalid invite code'));

                return $this->render('User/register.html.twig', ['form' => $form->createView()]);
            }

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