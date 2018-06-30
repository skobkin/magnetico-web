<?php

namespace App\Controller;

use App\Form\{CreateUserRequestType};
use App\FormRequest\CreateUserRequest;
use App\Repository\InviteRepository;
use App\User\{InviteManager, UserManager};
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Bundle\FrameworkBundle\Controller\Controller;
use Symfony\Component\Form\Extension\Core\Type\SubmitType;
use Symfony\Component\Form\FormInterface;
use Symfony\Component\HttpFoundation\{Request, Response};

class UserController extends Controller
{
    public function register(
        string $inviteCode,
        Request $request,
        EntityManagerInterface $em,
        UserManager $userManager,
        InviteManager $inviteManager,
        InviteRepository $inviteRepo
    ): Response {
        $createUserRequest = new CreateUserRequest($inviteCode);
        $form = $this->createRegisterForm($createUserRequest, $inviteCode);

        $invite = $inviteRepo->findOneBy(['code' => $inviteCode, 'usedBy' => null]);

        $form->handleRequest($request);

        if ($form->isSubmitted() && $form->isValid()) {
            $user = $userManager->createUserByInvite(
                $createUserRequest->username,
                $createUserRequest->password,
                $createUserRequest->email,
                $invite
            );

            $inviteManager->createInvitesForUser($user);

            $em->flush();

            return $this->redirectToRoute('index');
        }

        return $this->render('User/register.html.twig', [
            'form' => $form->createView(),
            'inviteValid' => $invite ? true : null,
        ]);
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